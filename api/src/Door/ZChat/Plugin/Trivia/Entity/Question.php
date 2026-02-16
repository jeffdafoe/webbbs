<?php

namespace App\Door\ZChat\Plugin\Trivia\Entity;

use App\Door\ZChat\Plugin\Trivia\Repository\QuestionRepository;
use App\Entity\Plugin;
use Doctrine\DBAL\Types\Types;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: QuestionRepository::class)]
#[ORM\Table(name: 'zchat_trivia_question')]
#[ORM\Index(columns: ['plugin_id', 'is_active'], name: 'idx_zchat_trivia_question_plugin')]
#[ORM\Index(columns: ['category'], name: 'idx_zchat_trivia_question_category')]
class Question
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private Plugin $plugin;

    #[ORM\Column(length: 100, nullable: true)]
    private ?string $category = null;

    #[ORM\Column(type: Types::TEXT)]
    private string $question;

    /** @var array<string> */
    #[ORM\Column(type: Types::JSON)]
    private array $answers = [];

    #[ORM\Column]
    private int $difficulty = 1;

    #[ORM\Column]
    private bool $isActive = true;

    #[ORM\Column]
    private int $timesAsked = 0;

    #[ORM\Column]
    private int $timesAnswered = 0;

    #[ORM\Column]
    private \DateTimeImmutable $createdAt;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->createdAt = new \DateTimeImmutable();
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getPlugin(): Plugin
    {
        return $this->plugin;
    }

    public function setPlugin(Plugin $plugin): static
    {
        $this->plugin = $plugin;
        return $this;
    }

    public function getCategory(): ?string
    {
        return $this->category;
    }

    public function setCategory(?string $category): static
    {
        $this->category = $category;
        return $this;
    }

    public function getQuestion(): string
    {
        return $this->question;
    }

    public function setQuestion(string $question): static
    {
        $this->question = $question;
        return $this;
    }

    /** @return array<string> */
    public function getAnswers(): array
    {
        return $this->answers;
    }

    /** @param array<string> $answers */
    public function setAnswers(array $answers): static
    {
        $this->answers = $answers;
        return $this;
    }

    public function checkAnswer(string $answer): bool
    {
        $normalizedAnswer = strtolower(trim($answer));
        foreach ($this->answers as $correctAnswer) {
            if (strtolower(trim($correctAnswer)) === $normalizedAnswer) {
                return true;
            }
        }
        return false;
    }

    public function getDifficulty(): int
    {
        return $this->difficulty;
    }

    public function setDifficulty(int $difficulty): static
    {
        $this->difficulty = max(1, min(5, $difficulty));
        return $this;
    }

    public function isActive(): bool
    {
        return $this->isActive;
    }

    public function setIsActive(bool $isActive): static
    {
        $this->isActive = $isActive;
        return $this;
    }

    public function getTimesAsked(): int
    {
        return $this->timesAsked;
    }

    public function incrementTimesAsked(): static
    {
        $this->timesAsked++;
        return $this;
    }

    public function getTimesAnswered(): int
    {
        return $this->timesAnswered;
    }

    public function incrementTimesAnswered(): static
    {
        $this->timesAnswered++;
        return $this;
    }

    public function getCreatedAt(): \DateTimeImmutable
    {
        return $this->createdAt;
    }
}
