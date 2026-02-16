<?php

namespace App\Door\ZChat\Plugin\Trivia\Entity;

use App\Door\ZChat\Plugin\Trivia\Repository\PlayerRepository;
use App\Entity\User;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: PlayerRepository::class)]
#[ORM\Table(name: 'zchat_trivia_player')]
#[ORM\UniqueConstraint(name: 'zchat_trivia_unique_game_player', columns: ['game_id', 'user_id'])]
class Player
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\ManyToOne(inversedBy: 'players')]
    #[ORM\JoinColumn(nullable: false)]
    private Game $game;

    #[ORM\ManyToOne(inversedBy: 'players')]
    #[ORM\JoinColumn(nullable: true)]
    private ?Team $team = null;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private User $user;

    #[ORM\Column]
    private int $personalScore = 0;

    #[ORM\Column]
    private int $correctAnswers = 0;

    #[ORM\Column]
    private \DateTimeImmutable $joinedAt;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->joinedAt = new \DateTimeImmutable();
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

    public function getTeam(): ?Team
    {
        return $this->team;
    }

    public function setTeam(?Team $team): static
    {
        $this->team = $team;
        return $this;
    }

    public function getUser(): User
    {
        return $this->user;
    }

    public function setUser(User $user): static
    {
        $this->user = $user;
        return $this;
    }

    public function getPersonalScore(): int
    {
        return $this->personalScore;
    }

    public function addScore(int $points): static
    {
        $this->personalScore += $points;
        return $this;
    }

    public function getCorrectAnswers(): int
    {
        return $this->correctAnswers;
    }

    public function incrementCorrectAnswers(): static
    {
        $this->correctAnswers++;
        return $this;
    }

    public function getJoinedAt(): \DateTimeImmutable
    {
        return $this->joinedAt;
    }

    public function recordCorrectAnswer(int $points = 1): static
    {
        $this->correctAnswers++;
        $this->personalScore += $points;

        if ($this->team !== null) {
            $this->team->addScore($points);
        }

        return $this;
    }
}
