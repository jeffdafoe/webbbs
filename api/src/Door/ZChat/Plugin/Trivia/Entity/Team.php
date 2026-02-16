<?php

namespace App\Door\ZChat\Plugin\Trivia\Entity;

use App\Door\ZChat\Plugin\Trivia\Repository\TeamRepository;
use Doctrine\Common\Collections\ArrayCollection;
use Doctrine\Common\Collections\Collection;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: TeamRepository::class)]
#[ORM\Table(name: 'zchat_trivia_team')]
class Team
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\ManyToOne(inversedBy: 'teams')]
    #[ORM\JoinColumn(nullable: false)]
    private Game $game;

    #[ORM\Column(length: 50)]
    private string $name;

    #[ORM\Column]
    private int $score = 0;

    /** @var Collection<int, Player> */
    #[ORM\OneToMany(targetEntity: Player::class, mappedBy: 'team')]
    private Collection $players;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->players = new ArrayCollection();
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getGame(): Game
    {
        return $this->game;
    }

    public function setGame(Game $game): static
    {
        $this->game = $game;
        return $this;
    }

    public function getName(): string
    {
        return $this->name;
    }

    public function setName(string $name): static
    {
        $this->name = $name;
        return $this;
    }

    public function getScore(): int
    {
        return $this->score;
    }

    public function addScore(int $points): static
    {
        $this->score += $points;
        return $this;
    }

    /** @return Collection<int, Player> */
    public function getPlayers(): Collection
    {
        return $this->players;
    }

    public function addPlayer(Player $player): static
    {
        if (!$this->players->contains($player)) {
            $this->players->add($player);
            $player->setTeam($this);
        }
        return $this;
    }

    public function getPlayerCount(): int
    {
        return $this->players->count();
    }
}
